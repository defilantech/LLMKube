/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"errors"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

	// routerProxyMetricsPort is the port the proxy exposes Prometheus
	// metrics on. Separate from the data-plane port so NetworkPolicies
	// can isolate scrape access.
	routerProxyMetricsPort int32 = 9090
)

// routerProxyResourceName is the canonical name used for every K8s
// object owned by a ModelRouter (Deployment, Service, ConfigMap). One
// helper keeps the naming consistent and the test fixtures predictable.
func routerProxyResourceName(modelRouterName string) string {
	return sanitizeDNSName(modelRouterName) + "-router-proxy"
}

// routerProxyServiceName is the data-plane Service's name:
// spec.proxy.serviceName when set (#2279), otherwise the generated
// routerProxyResourceName. Only that Service takes the custom name; every
// other router-proxy object, the pod labels, and the selectors keep the
// generated one, so a rename never touches the pods.
func routerProxyServiceName(mr *inferencev1alpha1.ModelRouter) string {
	if mr.Spec.Proxy != nil && mr.Spec.Proxy.ServiceName != "" {
		return mr.Spec.Proxy.ServiceName
	}
	return routerProxyResourceName(mr.Name)
}

// routerProxyAdminResourceName is the Service that carries the proxy's
// metrics/admin listener. It is separate from the data-plane Service so the
// admin endpoint stays ClusterIP regardless of spec.endpoint.type.
func routerProxyAdminResourceName(modelRouterName string) string {
	return routerProxyResourceName(modelRouterName) + "-admin"
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

// routerProxyNameTakenRequeueAfter is the bounded requeue for a name-taken
// refusal. The refusal depends on cluster state outside the router's spec and
// nothing watches the foreign object, so the requeue is the only way deleting
// or renaming it takes effect without an unrelated event. Same cadence as the
// AgentGateway plane's identical refusals (agentGatewayConflictRequeueAfter).
const routerProxyNameTakenRequeueAfter = 2 * time.Minute

// routerProxyNameTakenError reports a router-proxy object whose generated
// name is held by an existing object this ModelRouter does not control.
// Reconcile turns it into the RouterProxyNameTaken condition and a bounded
// requeue instead of a reconcile error.
type routerProxyNameTakenError struct {
	kind, ns, name, owner string
}

func (e *routerProxyNameTakenError) Error() string {
	if e.owner != "" {
		return fmt.Sprintf("%s %s/%s is already controlled by another ModelRouter (uid %s); "+
			"refusing to adopt or overwrite it; rename one of the routers", e.kind, e.ns, e.name, e.owner)
	}
	return fmt.Sprintf("%s %s/%s already exists and is not controlled by this ModelRouter; "+
		"refusing to adopt or overwrite it (LLMKube did not create it); delete or rename the object, or the router",
		e.kind, e.ns, e.name)
}

// ensureRouterProxyOwned is the single ownership gate every router-proxy
// upsert calls before overwriting an existing object: write only what this
// ModelRouter controls. An object with the generated name but no controller
// reference, a foreign reference, or another ModelRouter's reference is
// refused, never adopted, never overwritten, and never deleted (deletion is
// the non-Proxy sweep's job, also controlled-only), so a router can neither
// rewrite someone else's workload nor garbage-collect it.
func ensureRouterProxyOwned(mr *inferencev1alpha1.ModelRouter, existing metav1.Object, kind string) error {
	if metav1.IsControlledBy(existing, mr) {
		return nil
	}
	taken := &routerProxyNameTakenError{kind: kind, ns: existing.GetNamespace(), name: existing.GetName()}
	if ref := metav1.GetControllerOf(existing); ref != nil && ref.Kind == "ModelRouter" {
		// The owner ref's APIVersion is "<group>/<version>" (or bare version
		// for the core group); match the group so a same-Kind controller in
		// another API group is not misreported as another ModelRouter.
		if strings.SplitN(ref.APIVersion, "/", 2)[0] == inferencev1alpha1.GroupVersion.Group {
			taken.owner = string(ref.UID)
		}
	}
	return taken
}

// routerProxyNameTaken extracts a typed refusal from err, or nil. Lives here
// so callers that already import the apimachinery errors package do not need
// the stdlib one.
func routerProxyNameTaken(err error) *routerProxyNameTakenError {
	var taken *routerProxyNameTakenError
	if errors.As(err, &taken) {
		return taken
	}
	return nil
}
