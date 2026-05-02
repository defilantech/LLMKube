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

package controller

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Deployment construction. Turns an InferenceService + Model pair into the
// concrete Deployment that the controller applies to the cluster. The
// per-runtime backend (resolved via resolveBackend) contributes the container
// image, probes, arg/env list, and command. This file also owns the pod- and
// container-level security contexts that only the inference pod needs; the
// init-container security context stays in the controller file with its
// storage-builder callers.

// resolveEnableServiceLinks returns the value to set on PodSpec.EnableServiceLinks
// for a given backend. Backends that implement ServiceLinksOptOut and return
// true get an explicit `false`; everyone else gets nil (Kubernetes default,
// which is true). This keeps the legacy service-link env-var injection on for
// llama.cpp / generic / personaplex / tgi where it is harmless, and disables
// it for vLLM where the v0.20+ env-var validator turns it into log noise.
func resolveEnableServiceLinks(backend RuntimeBackend) *bool {
	if d, ok := backend.(ServiceLinksOptOut); ok && d.DisableServiceLinks() {
		f := false
		return &f
	}
	return nil
}

func inferPodSecurityContext(isvc *inferencev1alpha1.InferenceService) *corev1.PodSecurityContext {
	if isvc.Spec.PodSecurityContext != nil {
		return isvc.Spec.PodSecurityContext
	}
	return &corev1.PodSecurityContext{
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

func inferContainerSecurityContext(isvc *inferencev1alpha1.InferenceService) *corev1.SecurityContext {
	if isvc.Spec.SecurityContext != nil {
		return isvc.Spec.SecurityContext
	}
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
}

func (r *InferenceServiceReconciler) constructDeployment(
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
	replicas int32,
) *appsv1.Deployment {
	backend := resolveBackend(isvc)

	labels := map[string]string{
		"app":                           isvc.Name,
		"inference.llmkube.dev/model":   model.Name,
		"inference.llmkube.dev/service": isvc.Name,
	}

	image := backend.DefaultImage()
	if isvc.Spec.Image != "" {
		image = isvc.Spec.Image
	}

	port := backend.DefaultPort()
	if isvc.Spec.ContainerPort != nil {
		port = *isvc.Spec.ContainerPort
	} else if isvc.Spec.Endpoint != nil && isvc.Spec.Endpoint.Port > 0 {
		port = isvc.Spec.Endpoint.Port
	}

	skipInit := isvc.Spec.SkipModelInit != nil && *isvc.Spec.SkipModelInit

	var storageConfig modelStorageConfig
	var modelPath string
	if backend.NeedsModelInit() && !skipInit {
		useCache := model.Status.CacheKey != "" && r.ModelCachePath != ""
		storageConfig = buildModelStorageConfig(model, isvc, isvc.Namespace, useCache, r.CACertConfigMap, r.InitContainerImage)
		modelPath = storageConfig.modelPath
	}

	args := backend.BuildArgs(isvc, model, modelPath, port)

	startupProbe, livenessProbe, readinessProbe := backend.BuildProbes(port)
	if isvc.Spec.ProbeOverrides != nil {
		if isvc.Spec.ProbeOverrides.Startup != nil {
			startupProbe = isvc.Spec.ProbeOverrides.Startup
		}
		if isvc.Spec.ProbeOverrides.Liveness != nil {
			livenessProbe = isvc.Spec.ProbeOverrides.Liveness
		}
		if isvc.Spec.ProbeOverrides.Readiness != nil {
			readinessProbe = isvc.Spec.ProbeOverrides.Readiness
		}
	}

	container := corev1.Container{
		Name:            backend.ContainerName(),
		Image:           image,
		SecurityContext: inferContainerSecurityContext(isvc),
		Ports: []corev1.ContainerPort{
			{
				Name:          "http",
				ContainerPort: port,
				Protocol:      corev1.ProtocolTCP,
			},
		},
		VolumeMounts:   storageConfig.volumeMounts,
		StartupProbe:   startupProbe,
		LivenessProbe:  livenessProbe,
		ReadinessProbe: readinessProbe,
	}

	// Set command/args based on runtime
	if len(isvc.Spec.Command) > 0 {
		container.Command = isvc.Spec.Command
	} else if cb, ok := backend.(CommandBuilder); ok {
		container.Command = cb.BuildCommand()
	}
	if args != nil {
		container.Args = args
	}

	// Add runtime-generated env vars, then user-specified env vars (user wins on conflict)
	if eb, ok := backend.(EnvBuilder); ok {
		container.Env = append(container.Env, eb.BuildEnv(isvc)...)
	}
	if len(isvc.Spec.Env) > 0 {
		container.Env = append(container.Env, isvc.Spec.Env...)
	}

	gpuCount := resolveGPUCount(isvc, model)

	if gpuCount > 0 {
		container.Resources = corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				"nvidia.com/gpu": resource.MustParse(fmt.Sprintf("%d", gpuCount)),
			},
		}
	}

	if isvc.Spec.Resources != nil {
		if container.Resources.Limits == nil {
			container.Resources.Limits = corev1.ResourceList{}
		}
		if container.Resources.Requests == nil {
			container.Resources.Requests = corev1.ResourceList{}
		}
		if isvc.Spec.Resources.CPU != "" {
			container.Resources.Requests[corev1.ResourceCPU] = resource.MustParse(isvc.Spec.Resources.CPU)
		}
		if isvc.Spec.Resources.HostMemory != "" {
			container.Resources.Requests[corev1.ResourceMemory] = resource.MustParse(isvc.Spec.Resources.HostMemory)
		} else if isvc.Spec.Resources.Memory != "" {
			container.Resources.Requests[corev1.ResourceMemory] = resource.MustParse(isvc.Spec.Resources.Memory)
		}
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      isvc.Name,
			Namespace: isvc.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					SecurityContext:    inferPodSecurityContext(isvc),
					InitContainers:     storageConfig.initContainers,
					Containers:         []corev1.Container{container},
					Volumes:            storageConfig.volumes,
					PriorityClassName:  r.resolvePriorityClassName(isvc),
					RuntimeClassName:   isvc.Spec.RuntimeClassName,
					ImagePullSecrets:   isvc.Spec.ImagePullSecrets,
					EnableServiceLinks: resolveEnableServiceLinks(backend),
				},
			},
		},
	}

	if gpuCount > 0 {
		// Use Recreate strategy for GPU workloads to prevent deadlock:
		// RollingUpdate requires the new pod to be Ready before terminating the old,
		// but the new pod cannot schedule if the old pod holds the only available GPU(s).
		deployment.Spec.Strategy = appsv1.DeploymentStrategy{
			Type: appsv1.RecreateDeploymentStrategyType,
		}

		tolerations := []corev1.Toleration{
			{
				Key:      "nvidia.com/gpu",
				Operator: corev1.TolerationOpEqual,
				Value:    "present",
				Effect:   corev1.TaintEffectNoSchedule,
			},
		}

		if len(isvc.Spec.Tolerations) > 0 {
			tolerations = append(tolerations, isvc.Spec.Tolerations...)
		}

		deployment.Spec.Template.Spec.Tolerations = tolerations

		if len(isvc.Spec.NodeSelector) > 0 {
			deployment.Spec.Template.Spec.NodeSelector = isvc.Spec.NodeSelector
		}
	}

	return deployment
}
