/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Names, labels, and annotations shared across the three router-proxy
// builders (config, deployment, service). Pulled into a single file so
// the wire-shape contract is in one place.
const (
	// Backend tier strings shared between controller and proxy.
	backendTierLocal = "local"
	backendTierCloud = "cloud"

	// routerProxyComponent is the value of the standard k8s
	// app.kubernetes.io/component label applied to every owned object.
	routerProxyComponent = "router-proxy"

	// routerProxyContainerName is the container name inside the proxy
	// Deployment. Matched by ServiceMonitor selectors later.
	routerProxyContainerName = "router-proxy"

	// routerProxyConfigMountPath is where the controller mounts the
	// ConfigMap inside the proxy pod. Must match the binary's
	// --config default (see cmd/router-proxy/main.go).
	routerProxyConfigMountPath = "/etc/llmkube/router"

	// routerProxyConfigHashAnnotation is set on the Deployment pod
	// template so a ConfigMap content change triggers a rollout.
	routerProxyConfigHashAnnotation = "inference.llmkube.dev/router-config-hash"

	// routerProxyPort is the HTTP port the proxy listens on.
	routerProxyPort int32 = 8080
)

// routerProxyResourceName is the canonical name used for every K8s
// object owned by a ModelRouter (Deployment, Service, ConfigMap). One
// helper keeps the naming consistent and the test fixtures predictable.
func routerProxyResourceName(modelRouterName string) string {
	return sanitizeDNSName(modelRouterName) + "-router-proxy"
}

// routerProxyLabels are the standard k8s app labels applied to every
// owned object. The selectorLabels subset is used as the Deployment
// selector and Service selector and is immutable for the lifetime of
// the ModelRouter.
func routerProxyLabels(mr *inferencev1alpha1.ModelRouter) map[string]string {
	l := routerProxySelectorLabels(mr)
	l["app.kubernetes.io/name"] = "llmkube"
	l["app.kubernetes.io/component"] = routerProxyComponent
	l["app.kubernetes.io/managed-by"] = "llmkube-controller"
	return l
}

// routerProxySelectorLabels is the immutable label set used as both the
// Deployment podSelector and the Service selector. Keep it small and
// stable; downstream tooling depends on it.
func routerProxySelectorLabels(mr *inferencev1alpha1.ModelRouter) map[string]string {
	return map[string]string{
		"app":                                routerProxyResourceName(mr.Name),
		"inference.llmkube.dev/model-router": mr.Name,
	}
}
